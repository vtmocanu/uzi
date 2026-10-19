import { it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import http from "node:http";
import path from "node:path";
import { createHash, createHmac } from "node:crypto";
import type { AddressInfo } from "node:net";
import { RecoveryCoordinator, canonicalJson, type RecoveryRecord } from "../src/recovery.js";
import { WorkerClient } from "../src/client.js";
import { GitCache } from "../src/git.js";
import { makeFixture } from "./fixture-repo.js";
import { nullLogger, testGitCacheOptions } from "./helpers.js";

// Issue #3031: Worker.run's shutdown now joins RecoveryCoordinator.shutdown(). The default
// termination grace is 10 s under docker compose and 30 s on k8s, so a pass blocked in real
// work must abort and settle well inside the shorter one.
const BOUND_MS = 5_000;
const CEILING_MS = 12_000;
const token = "drain-bound-fixture";
const runId = "run-drain-bound";
const captureId = "00000000-0000-4000-8000-000000003031";

async function seal(recoveryRoot: string, record: RecoveryRecord): Promise<void> {
  const file = path.join(recoveryRoot, record.runId, record.captureId + ".json");
  await fs.mkdir(path.dirname(file), { recursive: true });
  const key = createHmac("sha256", token).update("uzi-recovery-journal-v1").digest();
  const mac = createHmac("sha256", key).update(canonicalJson(record)).digest("hex");
  await fs.writeFile(file, JSON.stringify({ ...record, mac }));
}

/** Time shutdown() to settlement, failing at a ceiling rather than hanging the suite. */
async function timedShutdown(coordinator: RecoveryCoordinator): Promise<number> {
  const started = performance.now();
  let timer: ReturnType<typeof setTimeout> | undefined;
  const ceiling = new Promise<"ceiling">(resolve => { timer = setTimeout(() => resolve("ceiling"), CEILING_MS); });
  const outcome = await Promise.race([coordinator.shutdown().then(() => "settled" as const), ceiling]);
  clearTimeout(timer);
  const elapsed = performance.now() - started;
  assert.equal(outcome, "settled", `shutdown join did not settle within ${CEILING_MS} ms`);
  return elapsed;
}

function within<T>(promise: Promise<T>, ms: number, what: string): Promise<T> {
  let timer: ReturnType<typeof setTimeout> | undefined;
  return Promise.race([promise, new Promise<T>((_, reject) => {
    timer = setTimeout(() => reject(new Error(`${what} not observed within ${ms} ms`)), ms);
  })]).finally(() => clearTimeout(timer));
}

it("#3031 shutdown aborts a boot pass blocked in a real archive upload to a silent server", async t => {
  const fx = makeFixture({}, { testName: t.name });
  t.after(() => fx.cleanup());
  const git = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions());
  let uploadEntered!: () => void;
  const entered = new Promise<void>(resolve => { uploadEntered = resolve; });
  let connectionClosed!: () => void;
  const closed = new Promise<void>(resolve => { connectionClosed = resolve; });
  const paths: string[] = [];
  // Real HTTP: every request but the upload is refused; the upload is read and never answered.
  const server = http.createServer((req, res) => {
    paths.push(req.url ?? "");
    if (req.url?.endsWith("/upload")) {
      res.on("close", connectionClosed);
      req.resume();
      uploadEntered();
      return;
    }
    res.writeHead(404, { "content-type": "application/json" }).end(JSON.stringify({ error: "not_found" }));
  });
  await new Promise<void>(resolve => server.listen(0, "127.0.0.1", resolve));
  t.after(() => { server.closeAllConnections(); return new Promise<void>(resolve => server.close(() => resolve())); });
  const baseUrl = `http://127.0.0.1:${(server.address() as AddressInfo).port}`;
  // Production default client timeout (30 s): only the coordinator's abort can end this upload early.
  const client = new WorkerClient(baseUrl, token, "0.1.0-test", nullLogger());
  const body = Buffer.from("# v2 git bundle\n" + "x".repeat(64 * 1024));
  const bundlePath = path.join(git.recoveryRoot, runId, captureId + ".bundle");
  await fs.mkdir(path.dirname(bundlePath), { recursive: true });
  await fs.writeFile(bundlePath, body);
  await seal(git.recoveryRoot, {
    version: 1, runId, generation: 1, captureId, sourceSha: "a".repeat(40), kind: "issue",
    branch: "agent/issue-3031", createdAt: 1, state: "bundled", bundlePath, byteSize: body.length,
    checksum: createHash("sha256").update(body).digest("hex"), chunkCount: 1, prerequisiteShas: [],
    selfContained: true, serverCaptureId: "server-capture",
  });
  const coordinator = new RecoveryCoordinator({ client, git, log: nullLogger(), recoveryRoot: git.recoveryRoot, workerToken: token });
  const pass = coordinator.resumePending();
  await within(entered, CEILING_MS, "upload request");
  const elapsed = await timedShutdown(coordinator);
  t.diagnostic(`upload shutdown join settled in ${elapsed.toFixed(1)} ms; requests ${JSON.stringify(paths)}`);
  assert.ok(elapsed < BOUND_MS, `join took ${elapsed} ms`);
  await within(closed, 1_000, "client-side close of the upload connection");
  await pass;
  // The journaled bundle stays for a later boot; nothing was uploaded or removed.
  assert.deepEqual(await fs.readFile(bundlePath), body);
});
