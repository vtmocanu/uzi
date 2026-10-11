import { it } from "node:test";
import assert from "node:assert/strict";
import http from "node:http";
import type { AddressInfo } from "node:net";
import { WorkerClient } from "../src/client.js";
import { nullLogger } from "./helpers.js";

type Budget = { signal?: AbortSignal; deadline: number };
for (const method of ["ownership", "capture"] as const) {
  const call = (client: WorkerClient, budget?: Budget) => method === "ownership"
    ? client.getRunOwnership("run", budget)
    : client.getRecoveryCaptureStatus("run", "capture", budget);

  for (const phase of ["headers", "success body", "error body"] as const) {
    for (const cancellation of ["deadline", "signal"] as const) {
      it(`${method}: actual stalled ${phase} refuses on ${cancellation}`, async t => {
        let entered!: () => void;
        const ready = new Promise<void>(resolve => { entered = resolve; });
        const server = http.createServer((_req, res) => {
          if (phase !== "headers") {
            res.writeHead(phase === "error body" ? 503 : 200, { "Content-Type": "application/json" });
            res.write('{"status":"cancelled"');
          }
          entered();
        });
        await new Promise<void>(resolve => server.listen(0, "127.0.0.1", resolve));
        t.after(async () => {
          server.closeAllConnections();
          await new Promise<void>(resolve => server.close(() => resolve()));
        });
        const client = new WorkerClient(`http://127.0.0.1:${(server.address() as AddressInfo).port}`, "fixture", "test", nullLogger());
        const abort = new AbortController();
        const pending = call(client, { signal: abort.signal, deadline: Date.now() + (cancellation === "deadline" ? 250 : 5000) });
        const rejected = assert.rejects(pending);
        await ready;
        if (cancellation === "signal") abort.abort(new Error("fixture cancelled"));
        await rejected;
      });
    }
  }

  for (const status of [200, 503]) {
    it(`${method}: actual UTF-8 bytes over 16 KiB refuse (${status})`, async t => {
      const server = http.createServer((_req, res) => {
        res.writeHead(status, { "Content-Type": "application/json" });
        res.end(JSON.stringify({ status: "cancelled", padding: "é".repeat(9000) }));
      });
      await new Promise<void>(resolve => server.listen(0, "127.0.0.1", resolve));
      t.after(async () => {
        server.closeAllConnections();
        await new Promise<void>(resolve => server.close(() => resolve()));
      });
      const client = new WorkerClient(`http://127.0.0.1:${(server.address() as AddressInfo).port}`, "fixture", "test", nullLogger());
      await assert.rejects(call(client, { deadline: Date.now() + 5000 }), /exceeds 16384 bytes/);
    });
  }

  for (const mode of ["expired", "cancelled"] as const) {
    it(`${method}: ${mode} request sends no authorization`, async () => {
      let sends = 0;
      const client = new WorkerClient("http://fixture.invalid", "fixture", "test", nullLogger(), {
        fetch: async () => { sends++; return Response.json({ status: "cancelled" }); },
      });
      const abort = new AbortController();
      if (mode === "cancelled") abort.abort();
      await assert.rejects(call(client, { signal: abort.signal, deadline: Date.now() + (mode === "expired" ? -100 : 5000) }));
      assert.equal(sends, 0);
    });
  }

  it(`${method}: default caller retains existing response contract`, async () => {
    const body = { status: "cancelled", padding: "x".repeat(17000) };
    const client = new WorkerClient("http://fixture.invalid", "fixture", "test", nullLogger(), {
      fetch: async () => Response.json(body),
    });
    if (method === "ownership") await assert.rejects(call(client), /exceeds 16384 bytes/);
    else assert.deepEqual(await call(client), body);
  });
}
