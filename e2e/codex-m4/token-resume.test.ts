import { test } from "node:test";
import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { mkdirSync, mkdtempSync, writeFileSync, rmSync } from "node:fs";
import path from "node:path";
import { once } from "node:events";
import { FakeProvider, dummyCredential } from "./fake-provider.js";
import { loadPackagedConfig, loadPackagedTransport, loadPackagedAppServerAuth } from "./packaged-modules.js";
import { resolveCodexBin, assertBinaryVersion } from "./provision.js";
import { deadline, message } from "../codex-m0/harness.mjs";
import { P_LAYER_SKIP } from "./p-platform.js";
import type { CodexNotification } from "../../agent/src/codex/transport.js";

// Actual uzi root posture, two app-server processes sharing only their isolated CODEX_HOME.
// The custom provider is HTTP-only loopback; no credential or network dependency.
test("pinned cold resume token characterization", { timeout: 90_000, skip: P_LAYER_SKIP }, async () => {
  const mods = { ...await loadPackagedTransport(), ...await loadPackagedAppServerAuth() };
  const config = await loadPackagedConfig();
  const bin = resolveCodexBin().codexBin;
  assertBinaryVersion(bin, "0.160.0");
  const scratch = path.resolve(process.cwd(), "../.uzi/scratch");
  mkdirSync(scratch, { recursive: true });
  const base = mkdtempSync(path.join(scratch, "token-resume-"));
  const home = path.join(base, "home");
  const cwd = path.join(base, "cwd");
  mkdirSync(home); mkdirSync(cwd);
  const credential = dummyCredential();
  const provider = await FakeProvider.start({
    credential,
    respond: () => [message("accounting fixture completed") as Record<string, unknown>],
    usage: (i) => ({
      input_tokens: i * 100, output_tokens: i * 30, total_tokens: i * 130,
      input_tokens_details: { cached_tokens: i * 20 },
      output_tokens_details: { reasoning_tokens: i * 10 },
    }),
  });
  writeFileSync(path.join(home, "config.toml"), config.buildCodexLoopbackTestConfigToml({
    model: "gpt-6-astra", projectPath: cwd, authMode: "api_key",
  }, provider.baseUrl));
  const all: { phase: number; note: CodexNotification }[] = [];
  let threadId = "";
  const wire: { phase: number; frame: Record<string, any> }[] = [];
  const metadata: { historyMode: string }[] = [];
  const turns: string[][] = [[], []];
  try {
    for (const phase of [0, 1]) {
      const child = spawn(bin, ["app-server"], {
        cwd, env: { HOME: home, CODEX_HOME: home, PATH: process.env.PATH },
        stdio: ["pipe", "pipe", "pipe"],
      });
      child.stderr.resume();
      let raw = "";
      child.stdout.on("data", (chunk: Buffer) => {
        raw += chunk.toString();
        let end: number;
        while ((end = raw.indexOf("\n")) !== -1) {
          wire.push({ phase, frame: JSON.parse(raw.slice(0, end)) });
          raw = raw.slice(end + 1);
        }
      });
      const transport = mods.createCodexTransport({ inbound: child.stdout, outbound: child.stdin });
      const notes = transport.notifications();
      await mods.createCodexAppServerAuth({ mode: "api_key", apiKey: credential }).authenticate(transport);
      try {
        const params = {
          model: "gpt-6-astra", modelProvider: config.CODEX_M3B_LOOPBACK_PROVIDER_NAME,
          cwd, approvalPolicy: "never",
          ephemeral: false, environments: [], dynamicTools: [],
          config: { project_doc_max_bytes: 0, projects: { [cwd]: { trust_level: "untrusted" } } },
          developerInstructions: "uzi offline accounting characterization",
        };
        const res = phase === 0
          ? await transport.request<{ thread: { id: string; historyMode: string } }>("thread/start", params)
          : await transport.request<{ thread: { id: string; historyMode: string } }>("thread/resume", {
            threadId, model: params.model, modelProvider: params.modelProvider, cwd,
            approvalPolicy: "never", config: params.config,
            developerInstructions: params.developerInstructions, excludeTurns: true,
          });
        metadata.push({ historyMode: res.thread.historyMode });
        assert.equal(res.thread.historyMode, "paginated");
        if (phase === 1) assert.equal(res.thread.id, threadId);
        threadId = res.thread.id;
        for (let n = 0; n < 2; n++) {
          const resTurn = await transport.request<{ turn: { id: string } }>("turn/start", {
            threadId, model: params.model, environments: [], input: [{ type: "text", text: "complete fixture" }],
          });
          turns[phase].push(resTurn.turn.id);
          await deadline((async () => {
            for (;;) {
              const step = await notes.next();
              assert.equal(step.done, false);
              const note = step.value!;
              all.push({ phase, note });
              if (note.kind === "activity" && note.requestId !== undefined)
                transport.respond(note.requestId, { error: { code: -32601, message: "fixture has no tools" } });
              if (note.kind === "turn_completed" && note.turnId === resTurn.turn.id) {
                assert.equal(note.status, "completed");
                return;
              }
            }
          })(), "characterization turn", 20_000);
        }
      } finally {
        const exited = once(child, "exit");
        await transport.close();
        child.kill("SIGKILL");
        await exited;
      }
    }
    const usage = all.filter(x => x.note.kind === "token_usage_updated");
    // Persist fixture-only evidence under scratch; retained after helper-owned session cleanup.
    const evidence = path.join(scratch, `token-resume-evidence-${Date.now()}.json`);
    console.log(JSON.stringify({ historyMode: metadata.map(m => m.historyMode), requests: provider.requests.length, recomputationRequests: provider.requests.length - 4, legacyModeProved: false, legacyRecoveryProved: false }));
    assert.equal(provider.requests.length, 4);
    assert.deepEqual(provider.errors, []);
    assert.equal(usage.length, 5);
    const buckets: Record<number, Record<string, number>> = {
      1: { inputTokens: 100, cachedInputTokens: 20, cacheWriteInputTokens: 0, outputTokens: 30, reasoningOutputTokens: 10, totalTokens: 130 },
      2: { inputTokens: 200, cachedInputTokens: 40, cacheWriteInputTokens: 0, outputTokens: 60, reasoningOutputTokens: 20, totalTokens: 260 },
      3: { inputTokens: 300, cachedInputTokens: 60, cacheWriteInputTokens: 0, outputTokens: 90, reasoningOutputTokens: 30, totalTokens: 390 },
      4: { inputTokens: 400, cachedInputTokens: 80, cacheWriteInputTokens: 0, outputTokens: 120, reasoningOutputTokens: 40, totalTokens: 520 },
      6: { inputTokens: 600, cachedInputTokens: 120, cacheWriteInputTokens: 0, outputTokens: 180, reasoningOutputTokens: 60, totalTokens: 780 },
      10: { inputTokens: 1000, cachedInputTokens: 200, cacheWriteInputTokens: 0, outputTokens: 300, reasoningOutputTokens: 100, totalTokens: 1300 },
    };
    const breakdown = (i: number) => buckets[i];
    const expected = [[0, 1, 1], [0, 3, 2], [1, 3, 2], [1, 6, 3], [1, 10, 4]];
    usage.forEach((entry, i) => {
      assert.equal(entry.phase, expected[i][0]);
      assert.ok(entry.note.kind === "token_usage_updated");
      assert.deepEqual(entry.note.usage.total, breakdown(expected[i][1]));
      assert.deepEqual(entry.note.usage.last, breakdown(expected[i][2]));
    });
    const resumed = wire.filter(entry => entry.phase === 1).map(entry => entry.frame);
    const responseIndex = resumed.findIndex(f => f.result?.thread?.id === threadId);
    const replayIndex = resumed.findIndex(f => f.method === "thread/tokenUsage/updated");
    const startIndex = resumed.findIndex(f => f.method === "turn/started" && f.params?.turn?.id === turns[1][0]);
    assert.ok(responseIndex >= 0 && replayIndex > responseIndex && startIndex > replayIndex);
    assert.equal(resumed[responseIndex].result.thread.historyMode, "paginated");
    assert.equal(resumed[replayIndex].params.turnId, turns[0][1]);
    const firstNew = resumed.find(f => f.method === "thread/tokenUsage/updated" && f.params?.turnId === turns[1][0]);
    assert.ok(firstNew);
    // This equality describes only the measured paginated fixture, never legacy recovery.
    for (const key of Object.keys(breakdown(1)))
      assert.equal(firstNew.params.tokenUsage.total[key] - firstNew.params.tokenUsage.last[key], resumed[replayIndex].params.tokenUsage.total[key]);
    assert.equal(provider.requests.length - 4, 0, "no provider recomputation observed");
    writeFileSync(evidence, JSON.stringify({
      version: "0.160.0", metadata, requests: 4, recomputationRequests: 0,
      legacyModeProved: false, legacyRecoveryProved: false,
      ordering: { responseIndex, replayIndex, startIndex },
      usage: usage.map(entry => entry.note.kind === "token_usage_updated" ? { phase: entry.phase, total: entry.note.usage.total, last: entry.note.usage.last } : {}),
    }, null, 2));
  } finally {
    await provider.close();
    rmSync(base, { recursive: true, force: true });
  }
});
