import { it } from "node:test";
import assert from "node:assert/strict";
import http from "node:http";
import { WorkerClient } from "../src/client.js";
import { nullLogger } from "./helpers.js";

for (const mode of ["exact", "overflow", "missing", "false", "unknown", "ordinary"] as const) {
 it("cross-check claim marker and streamed byte bound: " + mode, async () => {
  const prefix = JSON.stringify({ kind: mode === "false" || mode === "ordinary" ? "issue" : "cross_check" });
  const body = prefix + " ".repeat(mode === "exact" ? 2 * 1024 * 1024 - Buffer.byteLength(prefix)
   : mode === "overflow" ? 2 * 1024 * 1024 + 1 - Buffer.byteLength(prefix) : 0);
  const server = http.createServer((_req, res) => {
   if (mode !== "missing" && mode !== "ordinary") res.setHeader("X-Uzi-Claim-Kind", mode === "unknown" ? "future" : "cross_check");
   res.setHeader("Content-Type", "application/json");
   // No Content-Length: the cap must measure the streamed bytes.
   res.write(body.slice(0, 1024)); res.end(body.slice(1024));
  });
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  const client = new WorkerClient("http://127.0.0.1:" + (server.address() as { port: number }).port,
   "fixture-join", "test", nullLogger());
  try {
   if (mode === "exact" || mode === "ordinary") assert.equal((await client.claimRun())!.kind,
    mode === "exact" ? "cross_check" : "issue");
   else await assert.rejects(client.claimRun(), mode === "overflow" ? /exceeds/ : mode === "unknown" ? /unknown/ : /disagree/);
  } finally {
   server.closeAllConnections();
   await new Promise<void>((resolve, reject) => server.close((err) => err ? reject(err) : resolve()));
  }
 });
}

for (const sources of [
 { model_source: "pin", effort_source: "worker default" },
 { model_source: "worker default", effort_source: "pin" },
 { model_source: "pin" }, { effort_source: "pin" }, {},
] as const) {
 it("HTTP claim preserves independent provenance and legacy absence: " + JSON.stringify(sources), async () => {
  const body = { kind: "cross_check", config: { default_model: "gpt-6-astra", default_effort: "xhigh" },
   cross_check: { stage: "plan", ...sources } };
  const server = http.createServer((_req, res) => {
   res.setHeader("X-Uzi-Claim-Kind", "cross_check"); res.setHeader("Content-Type", "application/json");
   res.end(JSON.stringify(body));
  });
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  const client = new WorkerClient("http://127.0.0.1:" + (server.address() as { port: number }).port,
   "fixture-join", "test", nullLogger());
  try {
   const c = (await client.claimRun())!;
   assert.deepEqual(c.cross_check, body.cross_check);
   assert.deepEqual(c.config, body.config);
   assert.equal(Object.hasOwn(c.cross_check!, "model_source"), Object.hasOwn(sources, "model_source"));
   assert.equal(Object.hasOwn(c.cross_check!, "effort_source"), Object.hasOwn(sources, "effort_source"));
  } finally {
   server.closeAllConnections();
   await new Promise<void>((resolve, reject) => server.close((err) => err ? reject(err) : resolve()));
  }
 });
}
