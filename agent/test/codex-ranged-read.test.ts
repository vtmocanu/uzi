import { it } from "node:test";
import assert from "node:assert/strict";
import { spawn, spawnSync } from "node:child_process";
import { once } from "node:events";
import { mkdtemp, mkdir, writeFile, rm, rename, symlink } from "node:fs/promises";
import { resolve, join } from "node:path";
import { CodexCallbackBroker, type FileopRequest } from "../src/codex/broker.js";
import { FileopHelperClient } from "../src/codex/fileop-client.js";
import { ExecutionRegistry, newLocalExecutionEpoch } from "../src/codex/registry.js";

it("real helper read-first fallback uses only the second open after filesystem changes", { skip: process.platform !== "linux" ? "Linux openat2 helper" : false }, async () => {
  const root = resolve(import.meta.dirname, "../..");
  await mkdir(join(root, ".uzi/scratch"), { recursive: true });
  const scratch = await mkdtemp(join(root, ".uzi/scratch/ranged-read-"));
  const worktree = join(scratch, "worktree");
  const binary = join(scratch, "fileop");
  await mkdir(worktree);
  try {
    const build = spawnSync("go", ["build", "-o", binary, "./fileop"], {
      cwd: join(root, "agent/codex/supervisor"),
      env: { ...process.env, GOTOOLCHAIN: "local", GOFLAGS: "-buildvcs=false -mod=vendor" },
      timeout: 60000, encoding: "utf8",
    });
    assert.equal(build.status, 0, build.stderr);
    const helper = spawn(binary, ["--root", worktree], { stdio: ["pipe", "pipe", "pipe"] });
    const exited = once(helper, "exit");
    let stderr = "";
    helper.stderr.setEncoding("utf8");
    helper.stderr.on("data", chunk => { stderr += String(chunk); });
    const client = new FileopHelperClient({ inbound: helper.stdout, outbound: helper.stdin });
    const calls: FileopRequest[] = [];
    let between: (() => Promise<void>) | undefined;
    const broker = new CodexCallbackBroker({
      registry: new ExecutionRegistry(newLocalExecutionEpoch(1)),
      spawnCommand: async () => ({ code: 0, stdout: "", stderr: "" }),
      fileop: { op: async request => {
        calls.push(request);
        const response = await client.op(request);
        if (request.op === "read" && response.origin === "helper" && response.code === "E_OVERSIZE" && between) {
          const mutate = between; between = undefined;
          await mutate();
        }
        return response;
      } },
      worktreePath: worktree,
      screenPolicy: { extraSecretPaths: [join(worktree, "protected.log")] },
      grants: { role: "coder", phase: "implement", allowedTools: new Set(["Read"]), allowedSkills: new Set(), isRoot: true },
      allowedRoles: new Set(),
      delegate: async () => ({ ok: false, code: "child_failed", message: "unused" }),
    });
    let id = 0;
    const read = async (args: Record<string, unknown>) => {
      calls.length = 0;
      return broker.handleToolCall({ threadId: "t", turnId: "u", callId: String(++id) }, "Read", args, "root");
    };
    const path = join(worktree, "large.txt");
    const initial = "first\n" + "x".repeat(1024 * 1024);
    try {
      await writeFile(path, initial);
      const prefix = await read({ path: "large.txt", limit: 1 });
      assert.deepEqual(prefix, { ok: true, output: { size: Buffer.byteLength(initial), content: "first\n", offset: 1, linesReturned: 1, partialLastLine: false, truncated: true } });
      assert.deepEqual(calls, [{ op: "read", path: "large.txt" }, { op: "read_range", path: "large.txt", offset: 1, limit: 1 }]);
      const unranged = await read({ path: "large.txt" });
      assert.equal(unranged.ok, false);
      assert.deepEqual(calls.map(c => c.op), ["read"]);

      for (const mode of ["grow", "shrink", "replace", "delete", "symlink", "fifo", "binary", "prefix-only"] as const) {
        await rm(path, { force: true });
        await writeFile(path, initial);
        const next = mode === "grow" ? "new\n" + "z".repeat(2 * 1024 * 1024) : "small\n";
        between = async () => {
          if (mode === "replace") {
            await writeFile(join(worktree, "replacement"), next);
            await rename(join(worktree, "replacement"), path);
          } else if (mode === "grow" || mode === "shrink") await writeFile(path, next);
          else if (mode === "binary") await writeFile(path, Buffer.from([0, 10]));
          else if (mode === "prefix-only") await writeFile(path, Buffer.from("ok\n\u0000"));
          else {
            await rm(path);
            if (mode === "symlink") await symlink("target", path);
            if (mode === "fifo") {
              const fifo = spawnSync("mkfifo", [path], { timeout: 5000 });
              assert.equal(fifo.status, 0);
            }
          }
        };
        const result = await read({ path: "large.txt", limit: 1 });
        assert.deepEqual(calls.map(c => c.op), ["read", "read_range"], mode);
        if (mode === "grow" || mode === "shrink" || mode === "replace") {
          assert.deepEqual(result, { ok: true, output: {
            size: Buffer.byteLength(next), content: mode === "grow" ? "new\n" : next,
            offset: 1, linesReturned: 1, partialLastLine: false, truncated: mode === "grow",
          } }, mode);
        } else if (mode === "prefix-only") {
          assert.deepEqual(result, { ok: true, output: { size: 4, content: "ok\n", offset: 1,
            linesReturned: 1, partialLastLine: false, truncated: true } });
        } else {
          assert.equal(result.ok, false, mode);
          if (!result.ok) assert.equal(result.code, mode === "binary" ? "bad_args" : "fileop_denied");
        }
      }
      await rm(path, { force: true });
      await writeFile(path, "x".repeat(9 * 1024 * 1024));
      const incomplete = await read({ path: "large.txt", offset: 2 });
      assert.deepEqual(incomplete, { ok: true, output: { size: 9 * 1024 * 1024, content: "", offset: 2,
        linesReturned: 0, partialLastLine: false, truncated: true, incomplete: true, reason: "scan_budget", scannedBytes: 8 * 1024 * 1024 } });

      // Binary outside a small-file excerpt is still classified on the entire first read.
      for (const suffix of [Buffer.from([0]), Buffer.from([255])]) {
        await writeFile(path, Buffer.concat([Buffer.from("first\n"), suffix]));
        assert.equal((await read({ path: "large.txt", limit: 1 })).ok, false);
        assert.deepEqual(calls.map(c => c.op), ["read"]);
      }
      await writeFile(join(worktree, "protected.log"), initial);
      for (const denied of ["../outside", ".git/config", "protected.log"]) {
        assert.equal((await read({ path: denied, limit: 1 })).ok, false);
        assert.equal(calls.length, 0);
      }
    } finally {
      client.close();
      helper.stdin.end();
      const [code] = await exited;
      assert.equal(code, 0, stderr);
    }
  } finally {
    await rm(scratch, { recursive: true, force: true });
  }
});
