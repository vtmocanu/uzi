import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import fs from "node:fs/promises";
import path from "node:path";
import os from "node:os";

import { CodexCallbackBroker, type FileopClient, type FileopRequest, type RunGrants } from "../src/codex/broker.js";
import { buildFileopHelper, readBytesReply } from "./codex-search-fakes.js";
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

  it("searches a file over 1 MiB in chunks and reports a file whose reads disagree with its size as a partial file_size search", async () => {
    const limit = 1024 * 1024;
    const big = Buffer.concat([Buffer.alloc(limit + 1, "x"), Buffer.from("\nneedle")]);
    const searched = await new CodexCallbackBroker({
      registry: new ExecutionRegistry(newLocalExecutionEpoch(1)),
      spawnCommand: async () => { throw new Error("shell must not run"); },
      fileop: { op: async (request) => {
        if (request.op === "list") return { ok: true, entries: [{ name: "sample.txt", type: "file" }] };
        if (request.op === "stat") return { ok: true, size: big.length };
        if (request.op === "read_bytes") return readBytesReply(big, request);
        throw new Error(`unexpected operation: ${request.op}`);
      } },
      worktreePath: "/checkout",
      grants,
      delegate: async () => { throw new Error("delegation must not run"); },
    }).handleToolCall({ threadId: "t", turnId: "turn", callId: "1" }, "uzi_search", { query: "needle" }, "root");
    assert.deepEqual(searched, { ok: true, output: { matches: [{ path: "sample.txt", line: 2, text: "needle" }], truncated: false, bytes: big.length } });

    for (const reply of [
      // The first window reports a different size than the stat.
      (request: FileopRequest) => readBytesReply(Buffer.from("x"), request, { size: 2 }),
      // The window ends short of the stat size.
      (request: FileopRequest) => readBytesReply(Buffer.from("x"), request, { size: 5 }),
    ]) {
      const fileop: FileopClient = { op: async (request) => {
        if (request.op === "list") return { ok: true, entries: [{ name: "sample.txt", type: "file" }] };
        if (request.op === "stat") return { ok: true, size: 5 };
        if (request.op === "read_bytes") return reply(request);
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
      ), { ok: true, output: {
        // The 5-byte request is refunded down to the single byte actually returned.
        matches: [], truncated: true, bytes: 1, reason: "file_size", skippedFiles: 1,
        notice: "Partial search: coverage is incomplete; a missing match does not mean the text is absent.",
      } });
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
        if (request.op === "read_bytes") return readBytesReply(Buffer.from(content), request);
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
      const output = result.output as { matches: { text: string }[]; truncated: boolean; reason?: string; skippedFiles?: number; notice?: string };
      assert.equal(output.matches.length, count);
      assert.equal(output.truncated, truncated);
      if (truncated) {
        assert.equal(output.reason, "matches");
        assert.equal(output.skippedFiles, 0);
        assert.match(output.notice ?? "", /^Partial search: coverage is incomplete/);
      }
      if (content === lateLine) {
        assert.ok(output.matches[0]?.text.includes("needle"));
        assert.ok((output.matches[0]?.text.length ?? Infinity) <= 256);
      }
    }
  });

  it("executes broker reads through a required-Landlock fileop root without write authority", async (t) => {
    if (process.platform !== "linux") return t.skip("Landlock helper is Linux-only");
    const sandboxBin = "/usr/local/bin/uzi-codex-command-sandbox";
    try {
      await fs.access(sandboxBin);
    } catch {
      return t.skip("packaged sandbox binary unavailable");
    }
    // Search needs read_bytes, which a packaged helper may predate: build the helper from this tree.
    const built = await buildFileopHelper("readonly-helper-");
    if (!built) return t.skip("Linux openat2 helper");
    // The built binary lives under scratch, outside the sandbox's executable system paths, so the
    // sandbox root must contain it: the root is the build directory holding the binary, the
    // checkout and the private tmp. That makes the Landlock root (build dir) wider than the
    // helper root (checkout), unlike production. The outside file lives in a separate directory
    // the sandbox root does not contain.
    const base = built.dir;
    const checkout = path.join(base, "checkout");
    const privateTmp = path.join(base, "private");
    const outsideDir = await fs.mkdtemp(path.join(await fs.realpath(os.tmpdir()), "readonly-outside-"));
    const outside = path.join(outsideDir, "outside.txt");
    await fs.mkdir(checkout);
    await fs.mkdir(privateTmp, { mode: 0o700 });
    await fs.chmod(privateTmp, 0o700);
    await fs.writeFile(path.join(checkout, "sample.txt"), "needle\nsecond line\n");
    await fs.writeFile(outside, "outside secret");
    const child = spawn(sandboxBin, [
      "--root", base, "--tmp", privateTmp, "--cwd", checkout,
      "--mode", "required", "--", built.binary, "--root", checkout,
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
      assert.equal(overBytes.ok, true);
      if (overBytes.ok) assert.equal((overBytes.output as { truncated: boolean }).truncated, false);
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
      await fs.rm(outsideDir, { recursive: true, force: true });
      await fs.rm(built.dir, { recursive: true, force: true });
    }
  });
});
