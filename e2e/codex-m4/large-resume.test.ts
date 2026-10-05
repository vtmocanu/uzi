import { test } from "node:test";
import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { mkdirSync, mkdtempSync, writeFileSync, rmSync } from "node:fs";
import path from "node:path";
import { pathToFileURL } from "node:url";
import { once } from "node:events";
import { FakeProvider, dummyCredential } from "./fake-provider.js";
import { loadPackagedConfig, loadPackagedTransport, loadPackagedAppServerAuth } from "./packaged-modules.js";
import { resolveCodexBin, assertBinaryVersion } from "./provision.js";
import { deadline, message } from "../codex-m0/harness.mjs";
import { P_LAYER_SKIP } from "./p-platform.js";
import { recordEvidence } from "./evidence.js";
import { LARGE_RESUME_TITLE } from "./large-resume-title.js";
import type { CodexNotification } from "../../agent/src/codex/transport.js";

const CAP = 4 * 1024 * 1024;
const SENTINEL = "PRIOR-context-large-resume-sentinel";
// Normal persisted turns only. No session-file parsing or production cap overrides.
// The scripted provider proves server context restoration, never actual model memory.
test(LARGE_RESUME_TITLE, { timeout: 110_000, skip: P_LAYER_SKIP }, async () => {
  recordEvidence(LARGE_RESUME_TITLE, "fail");
  const mods = { ...await loadPackagedTransport(), ...await loadPackagedAppServerAuth() };
  const config = await loadPackagedConfig();
  const accounting = await import(pathToFileURL(path.resolve(process.env.CODEX_M4_SRC ?? "src", "codex/token-accounting.ts")).href) as typeof import("../../agent/src/codex/token-accounting.js");
  const bin = resolveCodexBin().codexBin;
  assertBinaryVersion(bin, "0.159.3");
  const scratch = path.resolve("../.uzi/scratch");
  mkdirSync(scratch, { recursive: true });
  const base = mkdtempSync(path.join(scratch, "large-resume-"));
  const home = path.join(base, "home"), cwd = path.join(base, "cwd");
  mkdirSync(home); mkdirSync(cwd);
  const credential = dummyCredential();
  const provider = await FakeProvider.start({
    credential, maxRequestBytes: 16 * 1024 * 1024,
    respond: (_body, p) => [message(p.requests.length <= 4
      ? `${SENTINEL}-${p.requests.length} ${"x".repeat(1200 * 1024)}`
      : "new resumed response") as Record<string, unknown>],
    usage: i => ({
      input_tokens: i * 100, output_tokens: i * 30, total_tokens: i * 130,
      input_tokens_details: { cached_tokens: i * 20 },
      output_tokens_details: { reasoning_tokens: i * 10 },
    }),
  });
  writeFileSync(path.join(home, "config.toml"), config.buildCodexLoopbackTestConfigToml({
    model: "gpt-6-astra", projectPath: cwd, authMode: "api_key",
  }, provider.baseUrl));
  const params = {
    model: "gpt-6-astra", modelProvider: config.CODEX_M3B_LOOPBACK_PROVIDER_NAME,
    cwd, approvalPolicy: "never", ephemeral: false, environments: [], dynamicTools: [],
    config: { project_doc_max_bytes: 0, projects: { [cwd]: { trust_level: "untrusted" } } },
    developerInstructions: "uzi offline large history characterization",
  };
  let threadId = "", lastOldTurn = "";
  let fullHistoryBytes = 0, metadataBytes = 0, warmMaxFrameBytes = 0, coldMaxFrameBytes = 0;
  const statuses: string[] = [];
  const open = async () => {
    const child = spawn(bin, ["app-server"], {
      cwd, env: { HOME: home, CODEX_HOME: home, PATH: process.env.PATH },
      stdio: ["pipe", "pipe", "pipe"],
    });
    child.stderr.resume();
    const exited = once(child, "exit");
    const transport = mods.createCodexTransport({ inbound: child.stdout, outbound: child.stdin });
    const notes = transport.notifications();
    const close = async () => {
      try { await transport.close(); } finally {
        if (child.exitCode === null && child.signalCode === null) child.kill("SIGKILL");
        await exited;
      }
    };
    try {
      await mods.createCodexAppServerAuth({ mode: "api_key", apiKey: credential }).authenticate(transport);
    } catch (error) { await close(); throw error; }
    return { child, transport, notes, close };
  };
  // One bounded byte-count spy, at most 16 MiB per line, never changes the real reader.
  const spy = (child: Awaited<ReturnType<typeof open>>["child"], onLine: (line: string, bytes: number) => void) => {
    let raw = "";
    child.stdout.on("data", (chunk: Buffer) => {
      raw += chunk.toString();
      assert.ok(Buffer.byteLength(raw) <= 16 * 1024 * 1024, "fixture spy bound");
      let end: number;
      while ((end = raw.indexOf("\n")) !== -1) {
        const line = raw.slice(0, end);
        onLine(line, Buffer.byteLength(line));
        raw = raw.slice(end + 1);
      }
    });
  };
  const finish = async (session: Awaited<ReturnType<typeof open>>, turnId: string) => {
    const observed: CodexNotification[] = [];
    await deadline((async () => {
      for (;;) {
        const step = await session.notes.next();
        assert.equal(step.done, false);
        const note = step.value!;
        observed.push(note);
        if (note.kind === "activity" && note.requestId !== undefined)
          session.transport.respond(note.requestId, { error: { code: -32601, message: "fixture has no tools" } });
        if (note.kind === "turn_completed" && note.turnId === turnId) {
          statuses.push(note.status!);
          assert.equal(note.status, "completed");
          return;
        }
      }
    })(), "large history turn", 20_000);
    return observed;
  };
  try {
    const warm = await open();
    try {
      spy(warm.child, (_line, bytes) => { warmMaxFrameBytes = Math.max(warmMaxFrameBytes, bytes); });
      const start = await warm.transport.request<{ thread: { id: string } }>("thread/start", params);
      threadId = start.thread.id;
      for (let i = 0; i < 4; i++) {
        const turn = await warm.transport.request<{ turn: { id: string } }>("turn/start", {
          threadId, model: params.model, environments: [], input: [{ type: "text", text: "persist this fixture turn" }],
        });
        lastOldTurn = turn.turn.id;
        await finish(warm, turn.turn.id);
      }
      assert.ok(warmMaxFrameBytes < CAP);
    } finally { await warm.close(); }

    // Independent cold full-history read: actual serialized history, not padding estimates.
    const full = await open();
    try {
      let readDone!: () => void;
      const readFrame = new Promise<void>(resolve => { readDone = resolve; });
      spy(full.child, (line, bytes) => {
        if (line.includes('"turns"') && line.includes(threadId) && line.includes(SENTINEL)) {
          fullHistoryBytes = bytes;
          readDone();
        }
      });
      await assert.rejects(full.transport.request("thread/read", { threadId, includeTurns: true }), /inbound frame exceeds size cap/);
      await deadline(readFrame, "full history byte measurement", 10_000);
      assert.ok(fullHistoryBytes > CAP);
    } finally { await full.close(); }

    const cold = await open();
    try {
      const wire: Record<string, any>[] = [];
      spy(cold.child, (line, bytes) => {
        coldMaxFrameBytes = Math.max(coldMaxFrameBytes, bytes);
        const frame = JSON.parse(line);
        wire.push(frame);
        if (frame.result?.thread?.id === threadId) metadataBytes = bytes;
      });
      const resumed = await cold.transport.request<{ thread: { id: string; historyMode: string; turns?: unknown[] } }>("thread/resume", {
        threadId, model: params.model, modelProvider: params.modelProvider, cwd,
        approvalPolicy: params.approvalPolicy, config: params.config,
        developerInstructions: params.developerInstructions, excludeTurns: true,
      });
      assert.equal(resumed.thread.id, threadId);
      assert.equal(resumed.thread.historyMode, "paginated");
      assert.deepEqual(resumed.thread.turns ?? [], []);
      assert.ok(metadataBytes > 0 && metadataBytes < CAP);
      assert.equal(provider.requests.length, 4, "resume itself does not recompute at provider");
      const next = await cold.transport.request<{ turn: { id: string } }>("turn/start", {
        threadId, model: params.model, environments: [], input: [{ type: "text", text: "continue saved context" }],
      });
      const notes = await finish(cold, next.turn.id);
      assert.equal(provider.requests.length, 5);
      assert.deepEqual(provider.errors, []);
      const restoredRequestBytes = Buffer.byteLength(JSON.stringify(provider.requests[4]));
      assert.ok(restoredRequestBytes > CAP && restoredRequestBytes < 16 * 1024 * 1024);
      const restoredInput = JSON.stringify(provider.requests[4].input);
      for (let i = 1; i <= 4; i++) assert.ok(restoredInput.includes(`${SENTINEL}-${i}`));
      const responseIndex = wire.findIndex(f => f.result?.thread?.id === threadId);
      const replayIndex = wire.findIndex(f => f.method === "thread/tokenUsage/updated" && f.params?.turnId === lastOldTurn);
      const startedIndex = wire.findIndex(f => f.method === "turn/started" && f.params?.turn?.id === next.turn.id);
      assert.ok(responseIndex >= 0 && replayIndex > responseIndex && startedIndex > replayIndex);
      const usage = notes.filter(n => n.kind === "token_usage_updated");
      assert.equal(usage.length, 2);
      const replay = usage[0], fresh = usage[1];
      assert.ok(replay.kind === "token_usage_updated" && fresh.kind === "token_usage_updated");
      assert.equal(replay.turnId, lastOldTurn);
      assert.equal(fresh.turnId, next.turn.id);
      assert.deepEqual(replay.usage.total, {
        inputTokens: 1000, cachedInputTokens: 200, cacheWriteInputTokens: 0,
        outputTokens: 300, reasoningOutputTokens: 100, totalTokens: 1300,
      });
      assert.deepEqual(fresh.usage.last, {
        inputTokens: 500, cachedInputTokens: 100, cacheWriteInputTokens: 0,
        outputTokens: 150, reasoningOutputTokens: 50, totalTokens: 650,
      });
      const acct = new accounting.CodexUsageAccountant();
      acct.registerThread(threadId, params.model, true);
      acct.record(threadId, replay.usage, true);
      assert.equal(acct.aggregateByModel(), undefined, "historical replay is not charged");
      acct.record(threadId, fresh.usage);
      assert.equal(acct.usageIncomplete, false);
      assert.deepEqual(acct.aggregateByModel(), { [params.model]: {
        inputTokens: 400, cacheReadInputTokens: 100, cacheCreationInputTokens: 0,
        outputTokens: 150, reasoningOutputTokens: 50, costStatus: "unreported",
      } });
      assert.ok(coldMaxFrameBytes < CAP);
      const evidence = {
        version: "0.159.3", nodeVersion: process.version, fullHistoryBytes, metadataBytes,
        warmMaxFrameBytes, coldMaxFrameBytes, restoredRequestBytes,
        statuses, historyMode: resumed.thread.historyMode, providerRequests: provider.requests.length,
        fullHistoryStatus: "production cap rejected", metadataResumeStatus: "success",
        serverContextRestored: true, modelMemoryProved: false, legacyRecoveryProved: false,
        ordering: { responseIndex, replayIndex, startedIndex }, charged: acct.aggregateByModel(),
      };
      writeFileSync(path.join(scratch, `large-resume-evidence-${Date.now()}.json`), JSON.stringify(evidence, null, 2));
      console.log(JSON.stringify(evidence));
    } finally { await cold.close(); }
  } finally {
    await provider.close();
    rmSync(base, { recursive: true, force: true });
  }
  recordEvidence(LARGE_RESUME_TITLE, "pass");
});
