import { it } from "node:test";
import assert from "node:assert/strict";
import { spawn, execFile } from "node:child_process";
import { once } from "node:events";
import fs from "node:fs/promises";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { promisify } from "node:util";
import { CodexCallbackBroker } from "../src/codex/broker.js";
import { FileopHelperClient } from "../src/codex/fileop-client.js";
import { ExecutionRegistry, newLocalExecutionEpoch } from "../src/codex/registry.js";

// Source-relative on purpose: copying this test to a base export builds that
// export's unchanged helper and exercises its public broker interface.
const repo = fileURLToPath(new URL("../../", import.meta.url));
let build: Promise<{ scratch: string; binary: string }> | undefined;
function buildHelper() {
  return build ??= (async () => {
    await fs.mkdir(path.join(repo, ".uzi/scratch"), { recursive: true });
    const scratch = await fs.mkdtemp(path.join(repo, ".uzi/scratch/large-read-"));
    const binary = path.join(scratch, "fileop");
    try {
      await promisify(execFile)("go", ["build", "-o", binary, "./fileop"], {
        cwd: path.join(repo, "agent/codex/supervisor"),
        env: { ...process.env, GOTOOLCHAIN: "local", GOFLAGS: "-mod=vendor -buildvcs=false" },
      });
      return { scratch, binary };
    } catch (error) {
      await fs.rm(scratch, { recursive: true, force: true });
      throw error;
    }
  })();
}

it("source-bound Linux large Read returns exact middle excerpt and enforces scan budget", { skip: process.platform !== "linux" }, async (t) => {
  const { scratch, binary } = await buildHelper();
  const checkout = path.join(scratch, "checkout");
  await fs.mkdir(checkout);
  const child = spawn(binary, ["--root", checkout], { stdio: ["pipe", "pipe", "pipe"] });
  const exited = once(child, "exit");
  const client = new FileopHelperClient({ inbound: child.stdout, outbound: child.stdin });
  try {
    const registry = new ExecutionRegistry(newLocalExecutionEpoch(1));
    const broker = new CodexCallbackBroker({
      registry, fileop: client, worktreePath: checkout,
      grants: { role: "read-only", phase: "implement", allowedTools: new Set(["Read"]), allowedSkills: new Set(), isRoot: true },
      spawnCommand: async () => { throw new Error("unexpected shell"); },
      delegate: async () => { throw new Error("unexpected delegation"); },
    });
    let call = 0;
    const read = (args: Record<string, unknown>) => broker.handleToolCall(
      { threadId: "t", turnId: "turn", callId: String(++call) }, "uzi_read", { path: "large.txt", ...args }, "root");
    const prefix = Buffer.alloc(65536, 112);
    const file = await fs.open(path.join(checkout, "large.txt"), "w");
    try {
      for (let i = 0; i < 17; i++) await file.write(prefix);
      await file.write("\nBODY é😀\r\nSUFFIX\n");
    } finally { await file.close(); }
    const size = 17 * prefix.length + Buffer.byteLength("\nBODY é😀\r\nSUFFIX\n");
    const result = await read({ offset: 2, limit: 1 });
    assert.equal(result.ok, true, JSON.stringify(result));
    if (result.ok) assert.deepEqual(result.output, {
      size, content: "BODY é😀\r\n", offset: 2, linesReturned: 1, partialLastLine: false, truncated: true,
    });
    const unranged = await read({});
    assert.equal(unranged.ok, false);
    assert.equal(unranged.fileopCode, "E_OVERSIZE");

    // Generate the production-bound smoke in chunks, without a file-sized string.
    const near = await fs.open(path.join(checkout, "large.txt"), "w");
    try {
      for (let i = 0; i < 1023; i++) await near.write(prefix);
      await near.write(Buffer.alloc(65535, 115));
    } finally { await near.close(); }
    let start = performance.now();
    const ranged = await read({ offset: 1 });
    t.diagnostic("near64MiB ranged ms=" + (performance.now() - start).toFixed(1));
    assert.equal(ranged.ok, true, JSON.stringify(ranged));
    if (ranged.ok) assert.deepEqual(ranged.output, {
      size: 64 * 1024 * 1024 - 1, content: "p".repeat(65536), offset: 1,
      linesReturned: 1, partialLastLine: true, truncated: true,
    });
    await fs.appendFile(path.join(checkout, "large.txt"), "s");
    assert.equal((await read({ limit: 200 })).ok, true);
    await fs.appendFile(path.join(checkout, "large.txt"), "s");
    const overBudget = await read({ offset: 1 });
    assert.equal(overBudget.ok, false);
    if (!overBudget.ok) assert.equal(overBudget.fileopCode, "E_OVERSIZE");
    await fs.writeFile(path.join(checkout, "large.txt"), "small\n");
    start = performance.now();
    const following = await read({});
    t.diagnostic("following legacy request ms=" + (performance.now() - start).toFixed(1));
    assert.equal(following.ok, true);
    await fs.writeFile(path.join(checkout, "large.txt"), Buffer.alloc(1024 * 1024, 97));
    start = performance.now();
    assert.equal((await read({})).ok, true);
    t.diagnostic("1MiB legacy ms=" + (performance.now() - start).toFixed(1));
    assert.equal(registry.inFlightCallbackCount(), 0);
  } finally {
    client.close();
    child.stdin.end();
    try { await exited; } finally { await fs.rm(scratch, { recursive: true, force: true }); }
  }
});
