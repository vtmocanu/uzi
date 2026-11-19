import test from "node:test";
import assert from "node:assert/strict";
import { mkdtempSync, mkdirSync, writeFileSync, readFileSync, rmSync, symlinkSync } from "node:fs";
import path from "node:path";
import { copySyntheticSessions, configProjection } from "./driver.js";
import { allowedInventory } from "./fake-provider.js";
import { buildCodexLoopbackTestConfigToml } from "../../agent/src/codex/config.js";

test("session adapter copies synthetic sessions and rejects symbolic links", () => {
  const base = mkdtempSync(path.resolve(__dirname, "../../.uzi/scratch/session-test-"));
  try {
    const source = path.join(base, "sessions");
    mkdirSync(source);
    writeFileSync(path.join(source, "session.jsonl"), "synthetic");
    copySyntheticSessions(source, path.join(base, "copy"));
    assert.equal(readFileSync(path.join(base, "copy/session.jsonl"), "utf8"), "synthetic");
    symlinkSync(path.join(source, "session.jsonl"), path.join(source, "link"));
    assert.throws(() => copySyntheticSessions(source, path.join(base, "rejected")), /symlink/);
  } finally { rmSync(base, { recursive: true, force: true }); }
});

test("destination and native assertions discriminate changed config", () => {
  const cwd = "/synthetic";
  const url = "http://127.0.0.1:12345/v1";
  const config = buildCodexLoopbackTestConfigToml({ model: "gpt-6-astra", projectPath: cwd, authMode: "api_key" }, url);
  assert.ok(Object.values(configProjection(config, cwd, url)).every(Boolean));
  assert.equal(configProjection(config.replace(url, "http://127.0.0.1:12346/v1"), cwd, url).destinationMatched, false);
  assert.equal(configProjection(config.replace("shell_tool = false", "shell_tool = true"), cwd, url).nativeConfigDisabled, false);
});

test("inventory rejects native top level, namespace and nested declaration", () => {
  assert.equal(allowedInventory({ tools: [{ type: "custom", name: "exec", description: "declare const tools: { uzi_bash(args: {}): Promise<unknown>; }" }] }), true);
  assert.equal(allowedInventory({ tools: [{ type: "function", name: "shell" }] }), false);
  assert.equal(allowedInventory({ tools: [{ type: "namespace", name: "clock", tools: [{ type: "function", name: "shell" }] }] }), false);
  assert.equal(allowedInventory({ tools: [{ type: "custom", name: "exec", description: "declare const tools: { exec_command(args: {}): Promise<unknown>; }" }] }), false);
});

test("inventory sees a second native function after an allowed member", () => {
  assert.equal(allowedInventory({ tools: [{ type: "custom", name: "exec", description: "declare const tools: { uzi_bash(args: {}): Promise<unknown>; apply_patch(args: {}): Promise<unknown>; }" }] }), false);
});
