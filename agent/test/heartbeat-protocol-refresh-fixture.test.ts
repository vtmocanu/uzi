import { it } from "node:test";
import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { mkdirSync, mkdtempSync, writeFileSync, rmSync } from "node:fs";
import { resolve, join } from "node:path";

const agent = resolve(import.meta.dirname, "..");

// Preload a transport into a child running the actual E2E fixture and real client.
// The source is lazy: a tee that drains the entire oversized body is observable.
const preload = `
const metrics = [];
process.on("exit", () => console.log("FIXTURE_PROBE " + JSON.stringify(metrics)));
globalThis.fetch = async (input) => {
  const path = new URL(String(input)).pathname;
  if (path.endsWith("/register")) return Response.json({
    worker_id: "fixture-worker", register_nonce: Buffer.alloc(32, 7).toString("base64url"),
    protocol_features: [],
  });
  if (path === "/__enable") return new Response(null, { status: 204 });
  if (!path.endsWith("/heartbeat")) throw new Error("unexpected probe request");
  const kind = process.env.PROBE_KIND;
  const limit = 1024 * 1024;
  const size = kind === "healthy" ? 2094 : kind === "exact" ? limit : kind === "extra" ? limit + 1 : limit * 2;
  const prefix = Buffer.from(JSON.stringify({ worker: { retaining_unpublished_work: false }, protocol_features: [] }));
  const m = { pulled: 0, eof: false, cancelled: false, cloneCancel: false, originalCancel: false };
  metrics.push(m);
  const response = new Response(new ReadableStream({
    pull(c) {
      if (kind === "failed" && m.pulled > 0) {
        c.error(new Error("untrusted-probe-secret"));
        return;
      }
      if (m.pulled === size) { m.eof = true; c.close(); return; }
      const chunk = Buffer.alloc(Math.min(65536, size - m.pulled), 32);
      if (m.pulled === 0) prefix.copy(chunk);
      m.pulled += chunk.length;
      c.enqueue(chunk);
    },
    cancel() { m.cancelled = true; },
  }, { highWaterMark: 0 }), { headers: { "Content-Length": "1" } });
  const clone = response.clone.bind(response);
  response.clone = () => {
    const copy = clone();
    // Response.clone replaces the original body with the other tee branch.
    const originalCancel = response.body.cancel.bind(response.body);
    response.body.cancel = (...args) => { m.originalCancel = true; return originalCancel(...args); };
    const getReader = copy.body.getReader.bind(copy.body);
    copy.body.getReader = (...args) => {
      const reader = getReader(...args);
      const cancel = reader.cancel.bind(reader);
      reader.cancel = (...args) => { m.cloneCancel = true; return cancel(...args); };
      return reader;
    };
    return copy;
  };
  return response;
};
`;

for (const kind of ["healthy", "exact", "oversized", "extra", "failed"] as const) {
  it(`actual refresh fixture bounds ${kind} response with lying content length`, () => {
    mkdirSync(resolve(agent, "../.uzi/scratch"), { recursive: true });
    const scratch = mkdtempSync(resolve(agent, "../.uzi/scratch/refresh-fixture-probe-"));
    try {
      const setup = join(scratch, "preload.mjs");
      writeFileSync(setup, preload);
      const childEnv = { ...process.env };
      delete childEnv.NODE_TEST_CONTEXT;
      const child = spawnSync(process.execPath, [
        "--import", "tsx", "--import", setup, "--test", "--test-reporter=tap",
        "--test-timeout=10000", "test/fixtures/heartbeat-protocol-refresh-e2e.ts",
      ], {
        cwd: agent, encoding: "utf8", timeout: 15000,
        env: { ...childEnv, TMPDIR: scratch, PROBE_KIND: kind,
          UZI_REFRESH_CLIENT: resolve(agent, "src/client.ts"),
          UZI_REFRESH_CASE: "new_worker_old_api",
          UZI_REFRESH_URL: "http://fixture.test", UZI_REFRESH_OLD_URL: "http://fixture.test",
          UZI_REFRESH_TOKEN: "fixture-join", UZI_REFRESH_RUN: "fixture-run",
          UZI_REFRESH_STATE: join(scratch, "state.json"),
        },
      });
      assert.equal(child.error, undefined);
      assert.equal(child.signal, null, "fixture must settle, including both tee cancellations");
      const output = child.stdout + child.stderr;
      const probe = output.match(/FIXTURE_PROBE (\[[^\n]+\])/);
      assert.ok(probe, output);
      const metrics = JSON.parse(probe[1]!) as {
        pulled: number; eof: boolean; cancelled: boolean; cloneCancel: boolean; originalCancel: boolean;
      }[];
      if (kind === "healthy" || kind === "exact") {
        assert.equal(child.status, 0, output);
        assert.equal(metrics.length, 5, "one request per heartbeat");
        for (const m of metrics) {
          assert.equal(m.pulled, kind === "exact" ? 1024 * 1024 : 2094);
          assert.equal(m.eof, true, "exact-limit body needs actual EOF");
          assert.equal(m.cancelled, false);
        }
        const wire = output.match(/M3_WIRE ([^\n]+)/);
        assert.ok(wire, output);
        const ticks = JSON.parse(wire[1]!).heartbeats as { responseBytes: number }[];
        assert.deepEqual(ticks.map(t => t.responseBytes), Array(5).fill(kind === "exact" ? 1024 * 1024 : 2094));
      } else {
        assert.notEqual(child.status, 0, output);
        assert.match(output, /fixture response unreadable or exceeds 1 MiB/);
        assert.doesNotMatch(output, /untrusted-probe-secret|M3_WIRE/);
        assert.equal(metrics.length, 1, "failure must stop the named fixture control");
        assert.equal(metrics[0]!.cloneCancel, true);
        assert.equal(metrics[0]!.originalCancel, true);
        assert.ok(metrics[0]!.pulled <= 1024 * 1024 + 3 * 65536, "tee prefetch must stay bounded");
        if (kind === "oversized") {
          assert.equal(metrics[0]!.eof, false, "must reject before draining the oversized response");
          assert.equal(metrics[0]!.cancelled, true, "both branches must stop the underlying source");
        }
      }
    } finally {
      rmSync(scratch, { recursive: true, force: true });
    }
  });
}
