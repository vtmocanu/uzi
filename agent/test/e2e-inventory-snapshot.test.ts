import assert from "node:assert/strict";
import { execFile } from "node:child_process";
import fs from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";
import { promisify } from "node:util";

const execute = promisify(execFile);
const collector = fileURLToPath(new URL("../../e2e/inventory-snapshot.mjs", import.meta.url));

test("E2E failure inventory emits identity metadata without authentication or terminal contents", async () => {
  const root = await fs.mkdtemp(path.join(os.tmpdir(), "cdr-inventory-metadata-"));
  const run = "b4dec5cf-e6d4-413b-b3a8-0005a2835767";
  const secret = "DO_NOT_EMIT_PRIVATE_RECORD_CONTENT";
  const recovery = path.join(root, "recovery", run);
  const outbox = path.join(root, "outbox", run);
  try {
    await fs.mkdir(recovery, { recursive: true });
    await fs.mkdir(outbox, { recursive: true });
    await fs.writeFile(path.join(recovery, "capture.json"), JSON.stringify({
      runId: run, generation: 3, state: "uploaded", inventoryGuarded: true,
      sourceSha: "a".repeat(40), coverageDigest: "b".repeat(64),
      mac: secret, token: secret, key: secret, bundlePath: secret,
      originalRoots: [{ sha: "c".repeat(40), contexts: [{ token: secret }] }],
      finalRequest: { evidence: "archive", mac: secret, disposition: {
        kind: "archive", capture_id: run, source_sha: "a".repeat(40),
        coverage_digest: "b".repeat(64), token: secret,
      } },
    }));
    await fs.writeFile(path.join(outbox, "terminal-3.json"), JSON.stringify({
      run_id: run, claim_generation: 3, mac: secret, signingKey: secret,
      body: { status: "completed", fail_origin: "worker", session_id: secret, plan: secret, token: secret },
    }));
    await fs.writeFile(path.join(outbox, "message-1.json"), JSON.stringify({ body: secret }));
    const { stdout, stderr } = await execute(process.execPath, [collector, path.dirname(recovery), path.dirname(outbox)]);
    assert.equal(stderr, "");
    assert.equal(stdout.includes(secret), false);
    const rows = stdout.trim().split("\n").map(line => JSON.parse(line));
    assert.equal(rows.length, 2);
    const archive = rows.find(row => row.kind === "recovery");
    const terminal = rows.find(row => row.kind === "terminal");
    assert.equal(archive.runId, run);
    assert.equal(archive.generation, 3);
    assert.deepEqual(archive.originalRootShas, ["c".repeat(40)]);
    assert.deepEqual(archive.finalRequest.disposition, {
      kind: "archive", capture_id: run, source_sha: "a".repeat(40), coverage_digest: "b".repeat(64),
    });
    assert.deepEqual(terminal.outcome, { status: "completed", fail_origin: "worker" });
    assert.equal(terminal.claim_generation, 3);
  } finally {
    await fs.rm(root, { recursive: true, force: true });
  }
});
