import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import fs from "node:fs/promises";
import path from "node:path";
import os from "node:os";

import { CodexCallbackBroker, type FileopClient, type RunGrants } from "../src/codex/broker.js";
import { buildCodexDynamicTools } from "../src/codex/dynamic-tools.js";
import { buildCodexProductionConfigToml } from "../src/codex/config.js";
import { wireFileopHelper } from "../src/codex/fileop-client.js";
import { ExecutionRegistry, newLocalExecutionEpoch } from "../src/codex/registry.js";

const grants: RunGrants = {
  role: "read-only",
  phase: "implement",
  allowedTools: new Set(["Read", "Search"]),
  allowedSkills: new Set(),
  isRoot: true,
};

describe("read-only Codex spike", () => {
  it("builds a tool-using, native-disabled turn with only read and bounded search", () => {
    const tools = buildCodexDynamicTools(grants);
    assert.deepEqual(tools.map((tool) => tool.name), ["uzi_read", "uzi_search"]);
    const config = buildCodexProductionConfigToml({
      model: "gpt-6-sol", authMode: "subscription", codeModeHost: true, projectPath: "/checkout",
    });
    assert.match(config, /project_doc_max_bytes = 0/);
    assert.match(config, /shell_tool = false/);
    assert.match(config, /code_mode_host = true/);
    assert.match(config, /trust_level = "untrusted"/);
  });

  it("reports oversized and truncated files as incomplete searches", async () => {
    for (const [size, truncated, expected] of [
      [64 * 1024 + 1, false, "Search file size limit reached"],
      [1, true, "Search file read incomplete"],
      [1, false, "Search file size limit reached"],
    ] as const) {
      const fileop: FileopClient = { op: async (request) => {
        if (request.op === "list") return { ok: true, entries: [{ name: "sample.txt", type: "file" }] };
        if (request.op === "stat") return { ok: true, size };
        if (request.op === "read") return { ok: true, data: Buffer.from(size === 1 && !truncated ? "x".repeat(64 * 1024 + 1) : "x").toString("base64"), truncated };
        throw new Error(`unexpected operation: ${request.op}`);
      } };
      const broker = new CodexCallbackBroker({
        registry: new ExecutionRegistry(newLocalExecutionEpoch(1)),
        spawnCommand: async () => { throw new Error("shell must not run"); },
        fileop,
        worktreePath: "/checkout",
        grants,
        delegate: async () => { throw new Error("delegation must not run"); },
      });
      assert.deepEqual(await broker.handleToolCall(
        { threadId: "t", turnId: "turn", callId: "1" }, "uzi_search", { query: "absent" }, "root",
      ), { ok: false, code: "search_limit", message: expected });
    }
  });

  it("includes a late match in its excerpt and truncates only when another match exists", async () => {
    const lateLine = `${"a".repeat(300)}needle${"b".repeat(100)}`;
    for (const [content, count, truncated] of [
      [lateLine, 1, false],
      [Array.from({ length: 20 }, () => "needle").join("\n"), 20, false],
      [Array.from({ length: 21 }, () => "needle").join("\n"), 20, true],
    ] as const) {
      const fileop: FileopClient = { op: async (request) => {
        if (request.op === "list") return { ok: true, entries: [{ name: "sample.txt", type: "file" }] };
        if (request.op === "stat") return { ok: true, size: Buffer.byteLength(content) };
        if (request.op === "read") return { ok: true, data: Buffer.from(content).toString("base64") };
        throw new Error(`unexpected operation: ${request.op}`);
      } };
      const broker = new CodexCallbackBroker({
        registry: new ExecutionRegistry(newLocalExecutionEpoch(1)),
        spawnCommand: async () => { throw new Error("shell must not run"); },
        fileop,
        worktreePath: "/checkout",
        grants,
        delegate: async () => { throw new Error("delegation must not run"); },
      });
      const result = await broker.handleToolCall(
        { threadId: "t", turnId: "turn", callId: "1" }, "uzi_search", { query: "needle" }, "root",
      );
      assert.equal(result.ok, true);
      if (!result.ok) continue;
      const output = result.output as { matches: { text: string }[]; truncated: boolean };
      assert.equal(output.matches.length, count);
      assert.equal(output.truncated, truncated);
      if (content === lateLine) {
        assert.ok(output.matches[0]?.text.includes("needle"));
        assert.ok((output.matches[0]?.text.length ?? Infinity) <= 256);
      }
    }
  });

  it("executes broker reads through a required-Landlock fileop root without write authority", async (t) => {
    if (process.platform !== "linux") return t.skip("Landlock helper is Linux-only");
    const sandboxBin = "/usr/local/bin/uzi-codex-command-sandbox";
    const fileopBin = "/usr/local/bin/uzi-codex-fileop";
    try {
      await fs.access(sandboxBin);
      await fs.access(fileopBin);
    } catch {
      return t.skip("packaged fileop and sandbox binaries unavailable");
    }
    const base = await fs.mkdtemp(path.join(await fs.realpath(os.tmpdir()), "readonly-"));
    const checkout = path.join(base, "checkout");
    const privateTmp = path.join(base, "private");
    const outside = path.join(base, "outside.txt");
    await fs.mkdir(checkout);
    await fs.mkdir(privateTmp, { mode: 0o700 });
    await fs.chmod(privateTmp, 0o700);
    await fs.writeFile(path.join(checkout, "sample.txt"), "needle\nsecond line\n");
    await fs.writeFile(outside, "outside secret");
    const child = spawn(sandboxBin, [
      "--root", checkout, "--tmp", privateTmp, "--cwd", checkout,
      "--mode", "required", "--", fileopBin, "--root", checkout,
    ], { cwd: checkout, env: { PATH: "/usr/bin:/bin", HOME: privateTmp, TMPDIR: privateTmp, LANG: "C" }, stdio: ["pipe", "pipe", "pipe"] });
    const handle = wireFileopHelper({ stdin: child.stdin, stdout: child.stdout });
    let stderr = "";
    child.stderr.setEncoding("utf8");
    child.stderr.on("data", (chunk: string) => { stderr += chunk; });
    try {
      const broker = new CodexCallbackBroker({
        registry: new ExecutionRegistry(newLocalExecutionEpoch(1)),
        spawnCommand: async () => { throw new Error("shell must not run"); },
        fileop: handle.client,
        worktreePath: checkout,
        grants,
        delegate: async () => { throw new Error("delegation must not run"); },
      });
      let call = 0;
      const invoke = (name: string, args: unknown) =>
        broker.handleToolCall({ threadId: "t", turnId: "turn", callId: String(++call) }, name, args, "root");
      const read = await invoke("uzi_read", { path: "sample.txt" });
      assert.equal(read.ok, true, JSON.stringify({ read, stderr, exitCode: child.exitCode, signalCode: child.signalCode }));
      if (read.ok) {
        assert.deepEqual(read.output, {
          size: Buffer.byteLength("needle\nsecond line\n"),
          content: "needle\nsecond line\n", offset: 1, linesReturned: 2,
          partialLastLine: false, truncated: false,
        });
      }
      const search = await invoke("uzi_search", { query: "needle" });
      assert.equal(search.ok, true, stderr);
      if (search.ok) assert.deepEqual((search.output as { matches: unknown }).matches, [{ path: "sample.txt", line: 1, text: "needle" }]);
      await fs.writeFile(path.join(checkout, "many.txt"), Array.from({ length: 25 }, () => "needle").join("\n"));
      const capped = await invoke("uzi_search", { query: "needle" });
      assert.equal(capped.ok, true);
      if (capped.ok) {
        assert.equal((capped.output as { matches: unknown[] }).matches.length, 20);
        assert.equal((capped.output as { truncated: boolean }).truncated, true);
      }
      for (let i = 0; i < 5; i++) await fs.writeFile(path.join(checkout, `large-${i}.txt`), "z".repeat(60 * 1024));
      const overBytes = await invoke("uzi_search", { query: "absent" });
      assert.deepEqual(overBytes, { ok: false, code: "search_limit", message: "Search byte limit reached" });
      const outsideRead = await invoke("uzi_read", { path: outside });
      assert.equal(outsideRead.ok, false);
      const denied = await Promise.all([
        invoke("uzi_bash", { command: "touch sample.txt" }),
        invoke("uzi_apply_patch", { path: "sample.txt", content: "changed" }),
        invoke("spawn_agent", { role: "coder" }),
      ]);
      assert.ok(denied.every((result) => !result.ok));
      assert.equal(await fs.readFile(path.join(checkout, "sample.txt"), "utf8"), "needle\nsecond line\n");
      assert.equal(await fs.readFile(outside, "utf8"), "outside secret");
    } finally {
      await handle.dispose();
      await new Promise<void>((resolve) => { if (child.exitCode !== null) resolve(); else child.once("exit", () => resolve()); });
      await fs.rm(base, { recursive: true, force: true });
    }
  });
});
