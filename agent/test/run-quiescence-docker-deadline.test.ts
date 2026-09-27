import { describe, it } from "node:test";
import assert from "node:assert/strict";
import http from "node:http";
import { teardownDocker } from "../src/run-quiescence.js";

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

  it("a listing whose body trickles forever is cut off at the overall deadline, not the inactivity timeout", async () => {
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

  it("a listing that sends headers then stalls is cut off at the overall deadline", async () => {
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
