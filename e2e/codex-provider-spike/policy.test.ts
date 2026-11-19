import fs from "node:fs";
import { syncBuiltinESMExports } from "node:module";
import test from "node:test";
import assert from "node:assert/strict";
import { mkdtempSync, mkdirSync, writeFileSync, readFileSync, rmSync, symlinkSync } from "node:fs";
import path from "node:path";
import { copySyntheticSessions, configProjection, observeCallbackRequest, withProtocolCleanup } from "./driver.js";
import { allowedInventory, callbackResultMatches } from "./fake-provider.js";
import { buildCodexLoopbackTestConfigToml } from "../../agent/src/codex/config.js";

test("session adapter copies synthetic sessions and rejects symbolic links", () => {
  mkdirSync(path.resolve(__dirname, "../../.uzi/scratch"), { recursive: true });
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

test("inventory rejects malformed identity fields while preserving nested containers", () => {
  for (const entry of [{ type: "function", name: null }, { type: null, name: "exec" },
    { type: 1, name: "exec" }, { type: "function", name: "" }, { name: ["exec"] }]) {
    assert.equal(allowedInventory({ tools: [entry] }), false, JSON.stringify(entry));
  }
  assert.equal(allowedInventory({ tools: [{ type: "function", function: { name: "uzi_bash" } }] }), true);
  assert.equal(allowedInventory({ tools: [{ tools: [{ type: "function", name: "uzi_bash" }] }] }), true);
});

test("callback result requires matching call id and synthetic success content", () => {
  const item = { type: "custom_tool_call_output", call_id: "spike-exec", output: [{ type: "text", text: 'Script completed\nOutput:\n{"code":0,"stdout":"synthetic","stderr":""}' }] };
  assert.equal(callbackResultMatches(item), true);
  assert.equal(callbackResultMatches({ ...item, call_id: "wrong" }), false);
  assert.equal(callbackResultMatches({ ...item, output: "" }), false);
  assert.equal(callbackResultMatches({ ...item, output: '{"code":0,"stdout":"wrong"}' }), false);
  assert.equal(callbackResultMatches({ ...item, output: '{"code":0.5,"stdout":"synthetic","stderr":""}' }), false);
  assert.equal(callbackResultMatches({ ...item, output: '{"code":1,"stdout":"synthetic","stderr":"","nested":{"code":0}}' }), false);
});

test("session directory enumeration stops before bulk allocation", () => {
  mkdirSync(path.resolve(__dirname, "../../.uzi/scratch"), { recursive: true });
  const base = mkdtempSync(path.resolve(__dirname, "../../.uzi/scratch/session-directory-test-"));
  const source = path.join(base, "sessions"); mkdirSync(source);
  for (let i = 0; i < 256; i++) writeFileSync(path.join(source, String(i)), "synthetic");
  const bulk = fs.readdirSync; const incremental = fs.opendirSync;
  let bulkEntries = 0, incrementalReads = 0;
  fs.readdirSync = ((...args: any[]) => {
    const entries = (bulk as any)(...args); bulkEntries += entries.length; return entries;
  }) as typeof fs.readdirSync;
  fs.opendirSync = ((...args: any[]) => {
    const directory = (incremental as any)(...args);
    const read = directory.readSync.bind(directory);
    directory.readSync = () => { incrementalReads++; return read(); };
    return directory;
  }) as typeof fs.opendirSync;
  syncBuiltinESMExports();
  try {
    assert.throws(() => copySyntheticSessions(source, path.join(base, "copy")), /session-copy-limit/);
    assert.equal(bulkEntries, 0);
    assert.ok(incrementalReads <= 128);
  } finally {
    fs.readdirSync = bulk; fs.opendirSync = incremental; syncBuiltinESMExports();
    rmSync(base, { recursive: true, force: true });
  }
});

test("session copy charges growth after stat before copying bytes", () => {
  mkdirSync(path.resolve(__dirname, "../../.uzi/scratch"), { recursive: true });
  const base = mkdtempSync(path.resolve(__dirname, "../../.uzi/scratch/session-growth-test-"));
  const source = path.join(base, "sessions"); mkdirSync(source);
  const file = path.join(source, "session.jsonl"); writeFileSync(file, "x");
  const original = fs.lstatSync;
  let injected = false;
  fs.lstatSync = ((...args: any[]) => {
    const stat = (original as any)(...args);
    if (String(args[0]) === file && !injected) {
      injected = true; writeFileSync(file, Buffer.alloc(4 * 1024 * 1024 + 1, 120));
    }
    return stat;
  }) as typeof fs.lstatSync;
  syncBuiltinESMExports();
  try {
    assert.throws(() => copySyntheticSessions(source, path.join(base, "copy")), /session-copy-limit/);
    assert.equal(injected, true);
    assert.ok(original(path.join(base, "copy/session.jsonl")).size <= 4 * 1024 * 1024);
  } finally { fs.lstatSync = original; syncBuiltinESMExports(); rmSync(base, { recursive: true, force: true }); }
});


test("shared advice request handler records attempts before rejecting without dispatch", () => {
  const observer = { attempts: 0 };
  const responses: unknown[] = [];
  const consumed = observeCallbackRequest({ kind: "activity", method: "item/tool/call", requestId: 7,
    params: { tool: "unexpected", arguments: {} } }, "advice", observer,
    (id, payload) => responses.push({ id, payload }));
  assert.equal(consumed, true);
  assert.equal(observer.attempts, 1);
  assert.deepEqual(responses, [{ id: 7, payload: { error: { code: -32601, message: "codex advice is tool-less" } } }]);
  assert.equal(observeCallbackRequest({ kind: "activity", method: "other", requestId: 8, params: {} }, "advice", observer,
    () => assert.fail("unrelated request")), false);
  assert.equal(observer.attempts, 1);
});

test("protocol and cleanup failures both survive, including success-only cleanup failure", async () => {
  const protocol = new Error("protocol-deadline");
  const cleanup = new Error("cleanup-failure");
  await assert.rejects(withProtocolCleanup(async () => { throw protocol; }, async () => { throw cleanup; }), error => {
    assert.ok(error instanceof AggregateError);
    assert.equal(error.message, protocol.message);
    assert.equal(error.cause, protocol);
    assert.deepEqual(error.errors, [protocol, cleanup]);
    return true;
  });
  await assert.rejects(withProtocolCleanup(async () => { throw protocol; }, async () => {}), error => error === protocol);
  await assert.rejects(withProtocolCleanup(async () => "ok", async () => { throw cleanup; }), error => error === cleanup);
  assert.equal(await withProtocolCleanup(async () => "ok", async () => {}), "ok");
});
