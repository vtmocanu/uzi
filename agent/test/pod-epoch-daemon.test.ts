import assert from "node:assert/strict";
import http from "node:http";
import type { ServerResponse } from "node:http";
import type { Socket } from "node:net";
import { once } from "node:events";
import { mkdir } from "node:fs/promises";
import { randomUUID } from "node:crypto";
import { relative } from "node:path";
import { setImmediate } from "node:timers/promises";
import { test } from "node:test";
import type { TestContext } from "node:test";
import { openPodEpochDaemonBarrier, PodEpochDaemonError } from "../src/pod-epoch-daemon.js";
import type { PodEpochDaemonGuard } from "../src/pod-epoch-daemon.js";

const cid = (n: number) => n.toString(16).padStart(64, "0");
type Route = (path: string, response: ServerResponse) => boolean | void;
async function daemon(t: TestContext, route: Route = () => false, count = 0, unix = false) {
  const calls: string[] = [], sockets = new Set<Socket>();
  let events: ServerResponse | undefined;
  const server = http.createServer((req, res) => {
    const path = req.url!;
    calls.push(req.method + " " + path);
    if (route(path, res)) return;
    if (path.startsWith("/events?")) { events = res; res.writeHead(200); res.flushHeaders(); return; }
    const data: Record<string, unknown> = {
      "/info": { Swarm: { LocalNodeState: "inactive" }, LiveRestoreEnabled: false },
      "/containers/json?all=1": Array.from({ length: count }, (_, n) => ({ Id: cid(n), State: "exited" })),
      "/volumes": { Volumes: [{ Driver: "local", Options: null }] },
      "/plugins": [],
    };
    if (path === "/_ping") { res.end("OK"); return; }
    const match = /^\/containers\/([a-f0-9]{64})\/json$/.exec(path);
    if (match) {
      res.setHeader("Content-Type", "application/json");
      res.end(JSON.stringify({ Id: match[1],
        State: { Running: false, Restarting: false, Paused: false, Status: "exited" },
        HostConfig: { RestartPolicy: { Name: "no" } } })); return;
    }
    if (Object.hasOwn(data, path)) { res.end(JSON.stringify(data[path])); return; }
    res.writeHead(404); res.end();
  });
  server.on("connection", socket => { sockets.add(socket); socket.on("close", () => sockets.delete(socket)); });
  const socketPath = relative(process.cwd(), new URL("../../.uzi/scratch/epoch-" + randomUUID() + ".sock", import.meta.url).pathname);
  if (unix) await mkdir(new URL("../../.uzi/scratch/", import.meta.url), { recursive: true });
  if (unix) server.listen(socketPath);
  else server.listen(0, "127.0.0.1");
  await once(server, "listening");
  const address = server.address(); assert.ok(address);
  t.after(async () => {
    for (const call of calls)
      assert.match(call, /^GET (?:\/_ping|\/info|\/containers\/json\?all=1|\/containers\/[a-f0-9]{64}\/json|\/volumes|\/plugins|\/events\?since=\d+)$/);
    for (const socket of sockets) socket.destroy();
    await new Promise<void>(resolve => server.close(() => resolve()));
  });
  const signal = new AbortController();
  const options = { dockerHost: typeof address === "string" ? "unix://" + address : "tcp://127.0.0.1:" + address.port,
    signal: signal.signal, deadline: Date.now() + 30_000 };
  return { calls, sockets, signal, options, get events() { return events; } };
}
async function turns(predicate: () => boolean) {
  for (let i = 0; i < 1000 && !predicate(); i++) await setImmediate();
  assert.ok(predicate(), "condition observed within bounded event loop turns");
}
async function refused(t: TestContext, path: string, payload: unknown, count = 0) {
  const d = await daemon(t, (p, res) => {
    if (p !== path) return false;
    res.end(JSON.stringify(payload)); return true;
  }, count);
  await assert.rejects(openPodEpochDaemonBarrier(d.options), PodEpochDaemonError);
  await turns(() => d.sockets.size === 0);
}
test("healthy inventory holds an unfiltered veto stream until close; all requests are GET", async t => {
  const d = await daemon(t, () => false, 8);
  const before = Math.floor(Date.now() / 1000);
  const guard = await openPodEpochDaemonBarrier(d.options);
  assert.equal(guard.signal.aborted, false); guard.assertHealthy();
  assert.equal(d.calls[0], "GET /_ping");
  assert.match(d.calls[1]!, /^GET \/events\?since=\d+$/);
  assert.ok(Number(d.calls[1]!.split("=")[1]) <= before + 1);
  assert.equal(d.calls.filter(c => /^GET \/containers\/[a-f0-9]/.test(c)).length, 8);
  d.events!.write(" \r\n\t\n");
  await setImmediate(); guard.assertHealthy();
  await guard.close(); await guard.close();
  assert.equal(guard.signal.aborted, true);
  assert.throws(() => guard.assertHealthy(), { reason: "daemon_closed" });
  await turns(() => d.sockets.size === 0);
});
test("fail closed on endpoint schemes and path injection without issuing a request", async () => {
  for (const dockerHost of ["http://localhost:2375", "https://localhost:2376", "tcp://a:1/path",
    "tcp://user@host:1", "tcp://host", "tcp://host:0", "unix://", "unix:///x?query"])
    await assert.rejects(openPodEpochDaemonBarrier({
      dockerHost, signal: new AbortController().signal, deadline: Date.now() + 1000,
    }), { reason: "daemon_endpoint" });
});
test("every daemon predicate and missing field refuses", async t => {
  const cases: Array<[string, unknown, number?]> = [
    ["/info", {}], ["/info", { Swarm: {}, LiveRestoreEnabled: false }],
    ["/info", { Swarm: { LocalNodeState: "active" }, LiveRestoreEnabled: false }],
    ["/info", { Swarm: { LocalNodeState: "inactive" }, LiveRestoreEnabled: true }],
    ["/info", { Swarm: { LocalNodeState: "inactive" } }],
    ["/containers/json?all=1", {}],
    ["/containers/json?all=1", [{ Id: "../info", State: "exited" }]],
    ["/containers/json?all=1", [{ Id: cid(0) }]],
    ["/containers/json?all=1", [{ Id: cid(0), State: "running" }]],
    ["/containers/json?all=1", [{ Id: cid(0), State: "paused" }]],
    ["/containers/json?all=1", [{ Id: cid(0), State: "restarting" }]],
    ["/containers/json?all=1", [{ Id: cid(0), State: "dead" }]],
    ["/containers/json?all=1", [{ Id: cid(0), State: "exited" }, { Id: cid(0), State: "exited" }]],
    ["/volumes", {}], ["/volumes", { Volumes: null }],
    ["/volumes", { Volumes: [{ Driver: "nfs", Options: {} }] }],
    ["/volumes", { Volumes: [{ Driver: "local" }] }],
    ["/volumes", { Volumes: [{ Driver: "local", Options: { device: "/data" } }] }],
    ["/plugins", {}], ["/plugins", [{}]], ["/plugins", [{ Enabled: true }]],
    ["/containers/json?all=1", Array.from({ length: 501 }, (_, n) => ({ Id: cid(n), State: "exited" }))],
    ["/volumes", { Volumes: Array.from({ length: 501 }, () => ({ Driver: "local", Options: {} })) }],
    ["/plugins", Array.from({ length: 51 }, () => ({ Enabled: false }))],
  ];
  for (const [index, [path, payload, count]] of cases.entries())
    await t.test(path + " " + index, sub => refused(sub, path, payload, count));
});
test("inventory element cap refuses before the oversized response finishes", async t => {
  const d = await daemon(t, (path, res) => {
    if (path !== "/containers/json?all=1") return false;
    const entries = Array.from({ length: 501 }, (_, n) => ({ Id: cid(n), State: "exited" }));
    res.write(JSON.stringify(entries).slice(0, -1));
    return true; // Deliberately never send EOF or the closing array delimiter.
  });
  await assert.rejects(openPodEpochDaemonBarrier(d.options), { reason: "daemon_inventory" });
  await turns(() => d.sockets.size === 0);
  assert.equal(d.calls.some(call => call.startsWith("GET /containers/") && call.endsWith("/json")), false);
});
test("inspect identity race, state and restart policy all refuse", async t => {
  const baseline = { Id: cid(0), State: { Running: false, Restarting: false, Paused: false, Status: "exited" },
    HostConfig: { RestartPolicy: { Name: "no" } } };
  const values: unknown[] = [{}, { ...baseline, Id: cid(1) }, { ...baseline, HostConfig: {} }];
  for (const field of ["Running", "Restarting", "Paused"])
    values.push({ ...baseline, State: { ...baseline.State, [field]: true } });
  for (const status of ["dead", "running", "paused", "restarting", "removing", "unknown"])
    values.push({ ...baseline, State: { ...baseline.State, Status: status } });
  values.push({ ...baseline, State: { Running: false, Status: "exited" } },
    { ...baseline, HostConfig: { RestartPolicy: { Name: "always" } } });
  for (const [n, value] of values.entries())
    await t.test(String(n), sub => refused(sub, "/containers/" + cid(0) + "/json", value, 1));
});
test("finite body caps, invalid UTF8, malformed and truncated responses refuse", async t => {
  for (const mode of ["length", "chunked", "malformed", "utf8", "truncated", "status", "ping"])
    await t.test(mode, async sub => {
      const d = await daemon(sub, (path, res) => {
        if (mode === "ping" && path === "/_ping") { res.end("no"); return true; }
        if (path !== "/info") return false;
        if (mode === "length") { res.writeHead(200, { "Content-Length": 8388609 }); res.flushHeaders(); }
        if (mode === "chunked") res.end(" ".repeat(8388609));
        if (mode === "malformed") res.end('{"Swarm":');
        if (mode === "utf8") res.end(Buffer.from([0xff]));
        if (mode === "truncated") { res.writeHead(200, { "Content-Length": 200 }); res.write("{}"); res.destroy(); }
        if (mode === "status") { res.writeHead(500); res.end("untrusted diagnostic"); }
        return true;
      });
      await assert.rejects(openPodEpochDaemonBarrier(d.options), error =>
        error instanceof PodEpochDaemonError && !error.message.includes("untrusted"));
      await turns(() => d.sockets.size === 0);
    });
});
test("events veto after inventory: any object, split UTF8, malformed, oversized, EOF or disconnect", async t => {
  for (const mode of ["object", "split", "malformed", "no-newline", "utf8", "oversize", "eof", "disconnect", "scalar"])
    await t.test(mode, async sub => {
      const d = await daemon(sub);
      const guard = await openPodEpochDaemonBarrier(d.options);
      sub.after(() => guard.close());
      if (mode === "object") d.events!.write("{}");
      if (mode === "split") {
        d.events!.write(Buffer.from('{"x":"'));
        d.events!.write(Buffer.from([0xc3])); d.events!.write(Buffer.from([0xa9]));
        d.events!.write('"}\n');
      }
      if (mode === "malformed") d.events!.write("{bad}\n");
      if (mode === "no-newline") d.events!.write("{bad}");
      if (mode === "utf8") d.events!.write(Buffer.from([0xff, 10]));
      if (mode === "oversize") d.events!.write(" ".repeat(65537));
      if (mode === "eof") d.events!.end();
      if (mode === "disconnect") d.events!.destroy();
      if (mode === "scalar") d.events!.write("[]\n");
      await turns(() => guard.signal.aborted);
      assert.throws(() => guard.assertHealthy(), PodEpochDaemonError);
      await guard.close(); await turns(() => d.sockets.size === 0);
    });
});
test("inspect workers join both external abort and event veto with queued work", async t => {
  for (const mode of ["event", "abort", "inspect-error"]) await t.test(mode, async sub => {
  const pending: ServerResponse[] = [];
  const d = await daemon(sub, (path, res) => {
    if (/^\/containers\/[a-f0-9]{64}\/json$/.test(path)) {
      pending.push(res); return true;
    }
    return false;
  }, 500);
  const opening = openPodEpochDaemonBarrier(d.options);
  // Attach rejection handler before triggering cancellation.
  const rejected = assert.rejects(opening, PodEpochDaemonError);
  await turns(() => pending.length === 4);
  await setImmediate(); assert.equal(pending.length, 4);
  if (mode === "event") d.events!.write('{"Action":"anything"}\n');
  if (mode === "abort") d.signal.abort();
  if (mode === "inspect-error") pending[0]!.end("{}");
  await rejected;
  await turns(() => d.sockets.size === 0);
  assert.equal(pending.length, 4);
  });
});
test("external abort joins requests blocked in stream headers or finite body", async t => {
  for (const mode of ["event-headers", "info-headers", "info-body"])
    await t.test(mode, async sub => {
      let blocked = false;
      const d = await daemon(sub, (path, res) => {
        if ((mode === "event-headers" && path.startsWith("/events?")) ||
            (mode !== "event-headers" && path === "/info")) {
          blocked = true;
          if (mode === "info-body") { res.writeHead(200); res.write("{"); }
          return true;
        }
        return false;
      });
      const opening = openPodEpochDaemonBarrier(d.options);
      const rejected = assert.rejects(opening, { reason: "daemon_aborted" });
      await turns(() => blocked); d.signal.abort(); await rejected;
      await turns(() => d.sockets.size === 0);
    });
});
test("unix socket and maximum inventory counts accept created stopped containers and empty options", async t => {
  const d = await daemon(t, (path, res) => {
    if (path === "/volumes") {
      res.end(JSON.stringify({ Volumes: Array.from({ length: 500 }, () => ({ Driver: "local", Options: {} })) }));
      return true;
    }
    if (path === "/plugins") {
      res.end(JSON.stringify(Array.from({ length: 50 }, () => ({ Enabled: false })))); return true;
    }
    if (/^\/containers\/[a-f0-9]{64}\/json$/.test(path)) {
      res.end(JSON.stringify({ Id: path.split("/")[2],
        State: { Running: false, Restarting: false, Paused: false, Status: "created" },
        HostConfig: { RestartPolicy: { Name: "no" } } })); return true;
    }
    return false;
  }, 500, true);
  const guard = await openPodEpochDaemonBarrier(d.options);
  guard.assertHealthy(); await guard.close();
  await turns(() => d.sockets.size === 0);
});
test("per-request timeout includes event headers and finite bodies and joins cancellation", async t => {
  for (const mode of ["headers", "body"]) await t.test(mode, async sub => {
    let blocked = false;
    const d = await daemon(sub, (path, res) => {
      if ((mode === "headers" && path.startsWith("/events?")) || (mode === "body" && path === "/info")) {
        blocked = true;
        if (mode === "body") res.write("{");
        return true;
      }
      return false;
    });
    const timers = new Map<() => void, number>();
    const opening = openPodEpochDaemonBarrier({ ...d.options,
      schedule: (fire, ms) => { timers.set(fire, ms); return () => { timers.delete(fire); }; },
    });
    const rejected = assert.rejects(opening, { reason: "daemon_timeout" });
    await turns(() => blocked);
    const timer = [...timers].find(([, ms]) => ms <= 10000); assert.ok(timer);
    timer[0](); await rejected;
    assert.equal(timers.size, 0);
    await turns(() => d.sockets.size === 0);
  });
});
test("already aborted or expired barriers send no requests", async t => {
  const d = await daemon(t);
  d.signal.abort();
  await assert.rejects(openPodEpochDaemonBarrier(d.options), { reason: "daemon_aborted" });
  await assert.rejects(openPodEpochDaemonBarrier({ ...d.options, signal: new AbortController().signal, deadline: 0 }),
    { reason: "daemon_deadline" });
  assert.equal(d.calls.length, 0);
});
test("injected deadline covers held stream without real waits", async t => {
  const d = await daemon(t);
  let clock = 1000;
  const timers = new Set<{ fire: () => void; ms: number }>();
  const guard: PodEpochDaemonGuard = await openPodEpochDaemonBarrier({
    ...d.options, now: () => clock, deadline: 601000,
    schedule: (fire, ms) => { const timer = { fire, ms }; timers.add(timer); return () => { timers.delete(timer); }; },
  });
  await turns(() => timers.size === 1);
  assert.equal([...timers][0]!.ms, 600000);
  clock = 601000; [...timers][0]!.fire();
  assert.throws(() => guard.assertHealthy(), { reason: "daemon_deadline" });
  await guard.close(); assert.equal(timers.size, 0);
  await turns(() => d.sockets.size === 0);
});

test("event byte bound counts cumulative small whitespace frames and joins stream close", async t => {
  const d = await daemon(t);
  const guard = await openPodEpochDaemonBarrier(d.options);
  t.after(() => guard.close());
  // Every individual frame is two bytes; newline bytes count toward the same bound.
  for (let i = 0; i < 7; i++) d.events!.write(" \n".repeat(4096));
  await setImmediate();
  guard.assertHealthy();
  d.events!.write(" \n".repeat(4096));
  d.events!.write("\n");
  await turns(() => guard.signal.aborted);
  assert.throws(() => guard.assertHealthy(), { reason: "daemon_event_invalid" });
  await guard.close();
  await turns(() => d.sockets.size === 0);
});
