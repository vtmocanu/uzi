import { describe, it } from "node:test";
import assert from "node:assert/strict";
import http from "node:http";
import { teardownDocker } from "../src/run-quiescence.js";

// Issue #2236: the teardown's DELETE must carry v=1, or Docker keeps the removed container's
// anonymous volumes and they leak onto the worker's dind-data volume.

interface Mount {
  Type: "bind" | "volume";
  Name?: string;
  Source: string;
  Destination: string;
}

/** A fake Engine API that models volume lifetime the way Docker does on container removal. */
function fakeDaemon() {
  const containers = new Map<string, { Id: string; Mounts: Mount[] }>();
  // volume name -> anonymous?
  const volumes = new Map<string, boolean>();
  const deletes: string[] = [];
  const vol = (name: string, dst: string): Mount => ({
    Type: "volume",
    Name: name,
    Source: `/var/lib/docker/volumes/${name}/_data`,
    Destination: dst,
  });
  const server = http.createServer((req, res) => {
    const url = new URL(req.url ?? "/", "http://docker");
    if (req.method === "GET" && url.pathname === "/containers/json") {
      res.writeHead(200, { "content-type": "application/json" });
      res.end(JSON.stringify([...containers.values()]));
      return;
    }
    const del = /^\/containers\/([^/]+)$/.exec(url.pathname);
    if (req.method === "DELETE" && del) {
      deletes.push(req.url ?? "");
      const id = decodeURIComponent(del[1]!);
      const c = containers.get(id);
      if (!c) {
        res.writeHead(404);
        res.end();
        return;
      }
      containers.delete(id);
      const v = url.searchParams.get("v");
      if (v === "1" || v === "true") {
        for (const m of c.Mounts) {
          if (m.Type !== "volume" || !m.Name || volumes.get(m.Name) !== true) continue;
          const inUse = [...containers.values()].some((o) => o.Mounts.some((om) => om.Name === m.Name));
          if (!inUse) volumes.delete(m.Name);
        }
      }
      res.writeHead(204);
      res.end();
      return;
    }
    res.writeHead(404);
    res.end();
  });
  return { server, containers, volumes, deletes, vol };
}

describe("teardownDocker removes an attributed container's anonymous volumes (#2236)", () => {
  it("removes the anonymous volume, keeps the named volume, and never touches a sibling", async () => {
    const d = fakeDaemon();
    d.volumes.set("anon-a", true);
    d.volumes.set("pgdata", false);
    d.volumes.set("anon-b", true);
    d.containers.set("A", {
      Id: "A",
      Mounts: [
        { Type: "bind", Source: "/clone/x", Destination: "/work" },
        d.vol("anon-a", "/cache"),
        d.vol("pgdata", "/var/lib/postgresql/data"),
      ],
    });
    d.containers.set("B", {
      Id: "B",
      Mounts: [{ Type: "bind", Source: "/elsewhere", Destination: "/work" }, d.vol("anon-b", "/cache")],
    });
    await new Promise<void>((resolve) => d.server.listen(0, "127.0.0.1", resolve));
    try {
      const { port } = d.server.address() as { port: number };
      const result = await teardownDocker({
        dockerHost: `tcp://127.0.0.1:${port}`,
        targetPaths: ["/clone"],
        sleep: async () => {},
      });
      assert.equal(result.state, "docker_unconfirmed", result.detail);
      assert.deepEqual(result.removed, ["A"]);
      assert.equal(d.containers.has("A"), false);
      assert.equal(d.volumes.has("anon-a"), false, "the removed container's anonymous volume leaked");
      assert.equal(d.volumes.has("pgdata"), true, "a named volume must survive");
      assert.equal(d.containers.has("B"), true, "a sibling container must survive");
      assert.equal(d.volumes.has("anon-b"), true, "a sibling's anonymous volume must survive");
      assert.deepEqual(d.deletes, ["/containers/A?force=1&v=1"]);
    } finally {
      d.server.closeAllConnections();
      await new Promise<void>((resolve, reject) => d.server.close((err) => (err ? reject(err) : resolve())));
    }
  });
});
